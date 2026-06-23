#include "ota.h"
#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include <errno.h>
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "esp_wifi.h"
#include "esp_log.h"
#include "esp_ota_ops.h"
#include "esp_app_desc.h"
#include "esp_partition.h"
#include "esp_http_client.h"
#include "esp_crt_bundle.h"
#include "esp_system.h"
#include "nvs.h"
#include "logbuf.h"
#include "mbedtls/sha256.h"

static const char *TAG = "ota";

/* Max failed attempts per target version. Protects against an OTA crash loop: if an
 * OTA reproducibly aborts (e.g. brownout while writing), the FW would otherwise retry
 * it on EVERY wake -> a permanent crash in battery field operation -> battery drained.
 * After MAX_OTA_TRIES it gives up on this version until the server offers a different one. */
#define MAX_OTA_TRIES 3

/* OTA download in RAM blocks: first receive ~32 KB (pure RX phase), then write it to
 * flash in one go + a short pause. Separates the WiFi RX burst in time from the flash
 * program peak -> the two current spikes no longer coincide (brownout mitigation on
 * weak power). While the block write runs the FW does not read -> the TCP window fills
 * -> the server pauses on its own, the supply recovers in the settle pause before the
 * next RX block draws the radio peak. */
#define OTA_BLOCK_SZ   (32 * 1024)
#define OTA_SETTLE_MS  40

/* --- Reset-proof diagnostics in NVS (survives reboot AND logbuf ring rotation) --- */
#define DIAG_NS "otadiag"

static void diag_set_stage(const char *stage)
{
    nvs_handle_t h;
    if (nvs_open(DIAG_NS, NVS_READWRITE, &h) == ESP_OK) {
        nvs_set_str(h, "stage", stage);
        nvs_commit(h);
        nvs_close(h);
    }
}

void ota_record_boot(void)
{
    nvs_handle_t h;
    if (nvs_open(DIAG_NS, NVS_READWRITE, &h) != ESP_OK) return;
    uint32_t boots = 0;
    nvs_get_u32(h, "boots", &boots);
    nvs_set_u32(h, "boots", boots + 1);
    uint8_t rr = (uint8_t)esp_reset_reason();
    nvs_set_u8(h, "rr", rr);   /* reset reason of THIS boot (also USB on the console reset) */

    /* If the previous run was stuck mid OTA phase, exactly THIS boot aborted it
     * -> freeze the reason + phase (ota_rr/stage), so that a later console reset
     * (rr=USB) no longer overwrites the crash cause. */
    char stage[40] = "";
    size_t sl = sizeof(stage);
    nvs_get_str(h, "stage", stage, &sl);
    bool mid_ota = stage[0] != '\0'
                && strncmp(stage, "boot-after:", 11) != 0
                && strcmp(stage, "done") != 0;
    if (mid_ota) {
        nvs_set_u8(h, "ota_rr", rr);
        char marked[56];
        snprintf(marked, sizeof(marked), "boot-after:%s", stage);
        nvs_set_str(h, "stage", marked);
    }
    nvs_commit(h);
    nvs_close(h);
}

bool ota_is_pending(void)
{
    const esp_partition_t *running = esp_ota_get_running_partition();
    esp_ota_img_states_t st;
    return esp_ota_get_state_partition(running, &st) == ESP_OK
        && st == ESP_OTA_IMG_PENDING_VERIFY;
}

void ota_mark_valid_if_pending(void)
{
    const esp_partition_t *running = esp_ota_get_running_partition();
    esp_ota_img_states_t st;
    if (esp_ota_get_state_partition(running, &st) == ESP_OK && st == ESP_OTA_IMG_PENDING_VERIFY) {
        esp_err_t err = esp_ota_mark_app_valid_cancel_rollback();
        ESP_LOGI(TAG, "fresh FW confirmed (mark_valid) -> %s [%s]",
                 running->label, esp_err_to_name(err));
        /* OTA succeeded -> clear the retry-backoff counter (clean base for the next OTA),
         * record the mark_valid counter + result for diagnostics. */
        nvs_handle_t h;
        if (nvs_open(DIAG_NS, NVS_READWRITE, &h) == ESP_OK) {
            nvs_erase_key(h, "try_ver");
            nvs_erase_key(h, "try_cnt");
            uint32_t mv = 0; nvs_get_u32(h, "mv", &mv);
            nvs_set_u32(h, "mv", mv + 1);
            nvs_set_u8(h, "mv_err", (uint8_t)err);
            nvs_commit(h);
            nvs_close(h);
        }
    }
}

/* <scheme>://<host>[:port]/<anything> -> <scheme>://<host>[:port]/firmware.bin.
 * Replaces the last path segment (typically "frame.bin") with "firmware.bin". */
static void derive_fw_url(const char *frame_url, char *out, size_t out_sz)
{
    const char *slash = strrchr(frame_url, '/');
    if (slash) {
        int base_len = (int)(slash - frame_url) + 1;   /* incl. the '/' */
        snprintf(out, out_sz, "%.*sfirmware.bin", base_len, frame_url);
    } else {
        snprintf(out, out_sz, "%s", frame_url);
    }
}

/* Expected SHA-256 (hex) of firmware.bin from the server header X-Firmware-SHA256.
 * Captured via the event handler during fetch_headers — esp_http_client_get_header does
 * NOT reliably deliver custom headers on the streaming path (an empty value would
 * otherwise silently skip the check -> fail-open gap). */
static char s_ota_sha[65];

static esp_err_t ota_http_evt(esp_http_client_event_t *e)
{
    if (e->event_id == HTTP_EVENT_ON_HEADER && e->header_key
        && strcasecmp(e->header_key, "X-Firmware-SHA256") == 0) {
        const char *v = e->header_value ? e->header_value : "";
        size_t n = strlen(v);
        if (n < sizeof(s_ota_sha)) memcpy(s_ota_sha, v, n + 1);
    }
    return ESP_OK;
}

bool ota_update_if_changed(const char *frame_url, const char *server_version)
{
    if (!server_version || server_version[0] == '\0')
        return false;   /* server delivers no FW version -> nothing to do */

    const esp_app_desc_t *running = esp_app_get_description();
    if (strncmp(running->version, server_version, sizeof(running->version)) == 0)
        return false;   /* already up to date */

    /* Retry backoff: count failed attempts per target version, give up after
     * MAX_OTA_TRIES. The counter is incremented BEFORE the download -> a brownout/crash
     * mid write (the function never returns) is still counted -> no endless loop. On
     * success ota_mark_valid_if_pending clears the counter again. */
    uint8_t tries = 0;
    nvs_handle_t bh;
    if (nvs_open(DIAG_NS, NVS_READWRITE, &bh) == ESP_OK) {
        char tried[40] = ""; size_t tl = sizeof(tried);
        nvs_get_str(bh, "try_ver", tried, &tl);
        nvs_get_u8(bh, "try_cnt", &tries);
        if (strcmp(tried, server_version) == 0) {
            if (tries >= MAX_OTA_TRIES) {
                ESP_LOGW(TAG, "gave up OTA to '%s' after %u failed attempts "
                              "(no retry loop; waiting for a different server version)",
                         server_version, tries);
                nvs_close(bh);
                return false;
            }
            tries++;
        } else {
            nvs_set_str(bh, "try_ver", server_version);
            tries = 1;
        }
        nvs_set_u8(bh, "try_cnt", tries);
        nvs_commit(bh);
        nvs_close(bh);
    }

    ESP_LOGI(TAG, "FW update: running '%s' != server '%s' -> pulling firmware.bin (attempt %u/%u)",
             running->version, server_version, tries, MAX_OTA_TRIES);
    diag_set_stage("start");

    char fw_url[224];
    derive_fw_url(frame_url, fw_url, sizeof(fw_url));

    s_ota_sha[0] = '\0';          /* reset before each attempt */
    esp_http_client_config_t cfg = {
        .url = fw_url,
        .timeout_ms = 30000,      /* ~1 MB download + 4 MB erase on ota_begin */
        .buffer_size = 2048,
        .buffer_size_tx = 3072,   /* like the frame pull: room for the X-Picpak-Log header */
        .keep_alive_enable = true,
        /* fw_url inherits the scheme from frame_url (derive_fw_url): https:// -> TLS
         * against the root bundle, http:// -> no-op. Same source as the frame pull. */
        .crt_bundle_attach = esp_crt_bundle_attach,
        .event_handler = ota_http_evt,   /* captures X-Firmware-SHA256 during fetch_headers */
    };
    esp_http_client_handle_t client = esp_http_client_init(&cfg);
    /* Send the FW-internal log ring along: the OTA path runs shortly before a reboot
     * that nulls the RTC-RAM -> the server is the most robust measurement channel. */
    static char fwlog[2100];
    if (logbuf_export(fwlog, sizeof(fwlog)) > 0)
        esp_http_client_set_header(client, "X-Picpak-Log", fwlog);

    /* Streaming like the official ota.rst example: open -> fetch_headers ->
     * read loop -> esp_ota_write. (NO esp_ota_write in the event callback.) */
    esp_err_t err = esp_http_client_open(client, 0);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "http_open: %s", esp_err_to_name(err));
        esp_http_client_cleanup(client);
        return false;
    }
    int64_t clen = esp_http_client_fetch_headers(client);
    int status = esp_http_client_get_status_code(client);
    if (status != 200) {
        ESP_LOGE(TAG, "http status %d (clen=%lld)", status, (long long)clen);
        esp_http_client_close(client);
        esp_http_client_cleanup(client);
        return false;
    }

    /* Fail-closed: without a valid 64-char SHA-256 header NO OTA — and check it BEFORE
     * the expensive partition erase. A missing/garbled hash must not let the integrity
     * check be skipped. */
    if (strlen(s_ota_sha) != 64) {
        ESP_LOGE(TAG, "missing/invalid X-Firmware-SHA256 header ('%s') -> OTA aborted", s_ota_sha);
        diag_set_stage("no-sha");
        esp_http_client_close(client);
        esp_http_client_cleanup(client);
        return false;
    }

    const esp_partition_t *update = esp_ota_get_next_update_partition(NULL);
    if (!update) {
        ESP_LOGE(TAG, "no OTA target partition found");
        esp_http_client_close(client);
        esp_http_client_cleanup(client);
        return false;
    }
    ESP_LOGI(TAG, "target %s @ 0x%lx, expecting %lld B, esp_ota_begin (partition erase)...",
             update->label, (unsigned long)update->address, (long long)clen);

    diag_set_stage("begin-erase");
    esp_ota_handle_t handle = 0;
    err = esp_ota_begin(update, OTA_SIZE_UNKNOWN, &handle);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "esp_ota_begin: %s", esp_err_to_name(err));
        diag_set_stage("begin-fail");
        esp_http_client_close(client);
        esp_http_client_cleanup(client);
        return false;
    }
    diag_set_stage("writing");

    /* Compute SHA-256 streamingly over the received bytes -> after the download compare
     * against the server hash (s_ota_sha, fail-closed checked above), BEFORE the
     * boot-set. Catches transport/brownout corruption. */
    mbedtls_sha256_context sha;
    mbedtls_sha256_init(&sha);
    mbedtls_sha256_starts(&sha, 0);   /* 0 = SHA-256 (not -224) */

    /* Brownout mitigation on the USB tether: WiFi RX (download) + SPI flash write
     * simultaneously create current spikes that pull the weak tether supply below the
     * brownout threshold (measured: reset_reason=9 in the writing phase). WiFi in
     * modem power-save -> lower continuous current; after each flash write a short
     * pause -> write peak and RX burst decoupled in time, supply recovers. Slows the
     * (rare) OTA download slightly; in battery field operation a harmless no-op surcharge. */
    esp_wifi_set_ps(WIFI_PS_MIN_MODEM);

    uint8_t *block = malloc(OTA_BLOCK_SZ);
    if (!block) {
        ESP_LOGE(TAG, "no RAM for OTA block (%d B)", OTA_BLOCK_SZ);
        mbedtls_sha256_free(&sha);
        esp_ota_abort(handle);
        esp_wifi_set_ps(WIFI_PS_NONE);
        return false;
    }
    int total = 0;
    bool read_ok = true;
    bool done = false;
    while (!done) {
        /* RX phase: fill the block without writing in between -> pure receive. */
        int filled = 0;
        while (filled < OTA_BLOCK_SZ) {
            int n = esp_http_client_read(client, (char *)block + filled, OTA_BLOCK_SZ - filled);
            if (n < 0) {
                ESP_LOGE(TAG, "read error at %d B (errno=%d)", total + filled, errno);
                read_ok = false;
                break;
            }
            if (n == 0) {
                /* EOF: complete -> done; otherwise abort before end. */
                if (esp_http_client_is_complete_data_received(client)) done = true;
                else {
                    ESP_LOGE(TAG, "connection aborted before end at %d B (errno=%d)",
                             total + filled, errno);
                    read_ok = false;
                }
                break;
            }
            filled += n;
        }
        if (!read_ok) break;
        /* Flash phase: write the whole block in one go. While the write runs the FW does
         * not read -> TCP window full -> server pauses -> modem RX ebbs. The settle pause
         * afterwards lets the supply recover before the next RX block draws the radio
         * peak -> RX burst and flash peak do not coincide. */
        if (filled > 0) {
            err = esp_ota_write(handle, block, filled);
            if (err != ESP_OK) {
                ESP_LOGE(TAG, "esp_ota_write: %s at %d B", esp_err_to_name(err), total);
                read_ok = false;
                break;
            }
            mbedtls_sha256_update(&sha, block, filled);
            total += filled;
            vTaskDelay(pdMS_TO_TICKS(OTA_SETTLE_MS));
        }
    }
    free(block);
    esp_wifi_set_ps(WIFI_PS_NONE);   /* take back power-save after the download */
    bool complete = esp_http_client_is_complete_data_received(client);
    esp_http_client_close(client);
    esp_http_client_cleanup(client);

    if (!read_ok || !complete) {
        ESP_LOGE(TAG, "download incomplete: %d B, read_ok=%d complete=%d -> abort",
                 total, read_ok, complete);
        diag_set_stage(read_ok ? "incomplete" : "read-fail");
        mbedtls_sha256_free(&sha);
        esp_ota_abort(handle);
        return false;
    }

    /* Finalize the SHA-256 over the received bytes and compare with the server hash
     * -> transport/brownout corruption surfaces here, BEFORE the boot-set. */
    unsigned char digest[32];
    mbedtls_sha256_finish(&sha, digest);
    mbedtls_sha256_free(&sha);
    char got_sha[65];
    for (int i = 0; i < 32; i++) snprintf(got_sha + i * 2, 3, "%02x", digest[i]);
    if (strcmp(s_ota_sha, got_sha) != 0) {
        ESP_LOGE(TAG, "SHA-256 MISMATCH (%d B): expected %.16s.. computed %.16s.. -> abort",
                 total, s_ota_sha, got_sha);
        diag_set_stage("sha-mismatch");
        esp_ota_abort(handle);
        return false;
    }
    ESP_LOGI(TAG, "SHA-256 verified: %.16s.. (%d B)", got_sha, total);

    diag_set_stage("end");
    err = esp_ota_end(handle);   /* validates magic/checksum/chip-ID of the image */
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "esp_ota_end (invalid image?): %s", esp_err_to_name(err));
        diag_set_stage("end-fail");
        return false;
    }
    diag_set_stage("setboot");
    err = esp_ota_set_boot_partition(update);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "esp_ota_set_boot_partition: %s", esp_err_to_name(err));
        diag_set_stage("setboot-fail");
        return false;
    }
    diag_set_stage("rebooting");
    ESP_LOGI(TAG, "OTA ok: %d B -> %s, reboot into the new FW", total, update->label);
    return true;
}

static const char *state_name(esp_ota_img_states_t st)
{
    return st == ESP_OTA_IMG_NEW            ? "NEW" :
           st == ESP_OTA_IMG_PENDING_VERIFY ? "PENDING_VERIFY" :
           st == ESP_OTA_IMG_VALID          ? "VALID" :
           st == ESP_OTA_IMG_INVALID        ? "INVALID" :
           st == ESP_OTA_IMG_ABORTED        ? "ABORTED" : "UNDEFINED";
}

size_t ota_diag_export(char *buf, size_t cap)
{
    const esp_partition_t *run = esp_ota_get_running_partition();
    const esp_app_desc_t  *desc = esp_app_get_description();
    const esp_partition_t *inv = esp_ota_get_last_invalid_partition();
    const esp_partition_t *p0 = esp_partition_find_first(ESP_PARTITION_TYPE_APP, ESP_PARTITION_SUBTYPE_APP_OTA_0, NULL);
    const esp_partition_t *p1 = esp_partition_find_first(ESP_PARTITION_TYPE_APP, ESP_PARTITION_SUBTYPE_APP_OTA_1, NULL);
    esp_ota_img_states_t sr = ESP_OTA_IMG_UNDEFINED, s0 = ESP_OTA_IMG_UNDEFINED, s1 = ESP_OTA_IMG_UNDEFINED;
    esp_ota_get_state_partition(run, &sr);
    if (p0) esp_ota_get_state_partition(p0, &s0);
    if (p1) esp_ota_get_state_partition(p1, &s1);
    uint32_t boots = 0, mv = 0; uint8_t rr = 0, ota_rr = 0, mv_err = 0;
    char stage[40] = "-"; size_t sl = sizeof(stage);
    nvs_handle_t h;
    if (nvs_open(DIAG_NS, NVS_READONLY, &h) == ESP_OK) {
        nvs_get_u32(h, "boots", &boots);
        nvs_get_u8(h, "rr", &rr);
        nvs_get_u8(h, "ota_rr", &ota_rr);
        nvs_get_u32(h, "mv", &mv);
        nvs_get_u8(h, "mv_err", &mv_err);
        nvs_get_str(h, "stage", stage, &sl);
        nvs_close(h);
    }
    return (size_t)snprintf(buf, cap,
        "run=%s/%s runstate=%d o0=%d o1=%d inv=%s boots=%lu rr=%u ota_rr=%u mv=%lu mv_err=%u stage=%s",
        run->label, desc->version, (int)sr, (int)s0, (int)s1, inv ? inv->label : "-",
        (unsigned long)boots, rr, ota_rr, (unsigned long)mv, mv_err, stage);
}

static void print_part_state(const char *label, esp_partition_subtype_t sub)
{
    const esp_partition_t *p = esp_partition_find_first(ESP_PARTITION_TYPE_APP, sub, NULL);
    esp_ota_img_states_t st = ESP_OTA_IMG_UNDEFINED;
    if (p) esp_ota_get_state_partition(p, &st);
    printf("  %s @ 0x%06lx  state=%s\r\n", label,
           p ? (unsigned long)p->address : 0, state_name(st));
}

void ota_print_status(void)
{
    const esp_partition_t *run  = esp_ota_get_running_partition();
    const esp_partition_t *boot = esp_ota_get_boot_partition();
    const esp_partition_t *next = esp_ota_get_next_update_partition(NULL);
    const esp_partition_t *inv  = esp_ota_get_last_invalid_partition();
    const esp_app_desc_t  *desc = esp_app_get_description();
    printf("running : %s @ 0x%06lx  v%s (%s %s)\r\n",
           run->label, (unsigned long)run->address, desc->version, desc->date, desc->time);
    printf("boot    : %s    next: %s    last_invalid: %s\r\n",
           boot ? boot->label : "?", next ? next->label : "?", inv ? inv->label : "-");
    print_part_state("ota_0", ESP_PARTITION_SUBTYPE_APP_OTA_0);
    print_part_state("ota_1", ESP_PARTITION_SUBTYPE_APP_OTA_1);
    /* Reset-proof OTA diagnostics from NVS */
    nvs_handle_t h;
    if (nvs_open(DIAG_NS, NVS_READONLY, &h) == ESP_OK) {
        uint32_t boots = 0, mv = 0; uint8_t rr = 0, ota_rr = 0, mv_err = 0;
        char stage[56] = "-"; size_t sl = sizeof(stage);
        nvs_get_u32(h, "boots", &boots);
        nvs_get_u8(h, "rr", &rr);
        nvs_get_u8(h, "ota_rr", &ota_rr);
        nvs_get_u32(h, "mv", &mv);
        nvs_get_u8(h, "mv_err", &mv_err);
        nvs_get_str(h, "stage", stage, &sl);
        nvs_close(h);
        printf("diag    : boots=%lu last_reset=%u  ota_reset=%u (3=SW 4=PANIC 5/6/7=WDT 9=BROWNOUT 11=USB)\r\n",
               (unsigned long)boots, rr, ota_rr);
        printf("          ota_stage=%s  mark_valid=%lux (err=%u)\r\n", stage, (unsigned long)mv, mv_err);
    }
}
